import type { Operation } from "@shared/types/operation";
import { TRANSFER_STAGES } from "@shared/types/operation";
import { bytes, duration, rate } from "@shared/utilities/format";

const STAGE_LABEL: Record<string, string> = {
  discovering: "Discovering",
  transferring: "Transferring",
  verifying: "Verifying",
  committing: "Committing",
  "cleaning-remote": "Cleaning remote source",
  complete: "Complete"
};

export function OperationProgress({ operation }: { operation: Operation }) {
  const stageIndex = operation.stage ? TRANSFER_STAGES.indexOf(operation.stage) : -1;

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
        <div
          style={{
            display: "flex", gap: 16, fontFamily: "var(--font-mono)",
            fontSize: "var(--text-sm)", color: "var(--text-2)"
          }}
        >
          {operation.bytesTotal ? (
            <span>{bytes(operation.bytesDone ?? 0) + " / " + bytes(operation.bytesTotal)}</span>
          ) : null}
          {operation.itemsTotal ? (
            <span>{operation.itemsDone + " / " + operation.itemsTotal + " artifacts"}</span>
          ) : null}
          {operation.bytesPerSecond ? <span>{rate(operation.bytesPerSecond)}</span> : null}
          {operation.etaSeconds ? <span>{duration(operation.etaSeconds) + " remaining"}</span> : null}
          <span style={{ color: "var(--text)", fontWeight: 600 }}>{operation.percent + "%"}</span>
        </div>
      </div>

      <div
        role="progressbar"
        aria-valuemin={0}
        aria-valuemax={100}
        aria-valuenow={operation.percent}
        aria-label={operation.label + " \u2014 " + operation.setName}
        style={{ height: 6, borderRadius: 3, background: "var(--surface-3)", overflow: "hidden" }}
      >
        <div
          style={{
            width: operation.percent + "%", height: "100%",
            background: operation.nonDestructive ? "var(--text-3)" : "var(--accent)"
          }}
        />
      </div>

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

      <p style={{ margin: 0, fontSize: "var(--text-sm)", color: "var(--text-3)" }}>
        {operation.nonDestructive
          ? "Read-only pass \u2014 no artifacts are deleted during this operation."
          : "The remote artifact is removed only after the NAS copy is verified and durably committed."}
      </p>
    </div>
  );
}
