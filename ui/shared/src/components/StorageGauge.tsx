import { bytes } from "@shared/utilities/format";

export function StorageGauge({
  freeBytes,
  totalBytes,
  state
}: {
  freeBytes: number;
  totalBytes: number;
  state: "nominal" | "warning" | "critical";
}) {
  const usedFraction = 1 - freeBytes / totalBytes;
  const color =
    state === "nominal" ? "var(--ok)" : state === "warning" ? "var(--warn)" : "var(--danger)";

  return (
    <div>
      <div
        role="meter"
        aria-valuemin={0}
        aria-valuemax={100}
        aria-valuenow={Math.round(usedFraction * 100)}
        aria-label="Storage used"
        style={{
          height: 5, borderRadius: 3, background: "var(--surface-3)", overflow: "hidden"
        }}
      >
        <div style={{ width: (usedFraction * 100).toFixed(1) + "%", height: "100%", background: color }} />
      </div>
      <div style={{ marginTop: 5, fontSize: "var(--text-sm)", color: "var(--text-2)" }}>
        {bytes(totalBytes - freeBytes) + " of " + bytes(totalBytes) + " used \u00b7 " +
          Math.round(usedFraction * 100) + "%"}
      </div>
    </div>
  );
}
