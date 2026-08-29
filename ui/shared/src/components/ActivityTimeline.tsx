import type { ActivityEvent, Severity } from "@shared/types/operation";
import { stamp } from "@shared/utilities/format";

export const SEVERITY: Record<Severity, { glyph: string; color: string }> = {
  ok: { glyph: "\u2713", color: "var(--ok)" },
  info: { glyph: "\u00b7", color: "var(--text-3)" },
  warn: { glyph: "\u25b2", color: "var(--warn)" },
  error: { glyph: "\u2715", color: "var(--danger)" }
};

export function ActivityTimeline({
  events,
  dense = false
}: {
  events: ActivityEvent[];
  dense?: boolean;
}) {
  return (
    <ul style={{ margin: 0, padding: 0, listStyle: "none" }}>
      {events.map((e) => {
        const sev = SEVERITY[e.severity];
        return (
          <li
            key={e.id}
            style={{
              display: "grid",
              gridTemplateColumns: dense ? "62px 14px 1fr" : "132px 16px 1fr auto",
              gap: dense ? 10 : 14,
              alignItems: "baseline",
              padding: dense ? "0 0 9px" : "11px 16px",
              borderBottom: dense ? undefined : "1px solid var(--border)",
              fontSize: dense ? "var(--text-sm)" : 13
            }}
          >
            <span
              className="mono"
              style={{ color: "var(--text-3)", fontSize: "var(--text-sm)" }}
            >
              {dense ? stamp(e.at).slice(7) : stamp(e.at)}
            </span>
            <span aria-hidden="true" style={{ color: sev.color, textAlign: "center" }}>
              {sev.glyph}
            </span>
            <span>
              <span style={{ fontWeight: e.severity === "warn" || e.severity === "error" ? 600 : 400 }}>
                {e.text}
              </span>{" "}
              <span style={{ color: "var(--text-3)" }}>{e.detail}</span>
            </span>
            {dense ? null : (
              <span style={{ fontSize: "var(--text-sm)", color: "var(--text-3)" }}>{e.setName}</span>
            )}
          </li>
        );
      })}
    </ul>
  );
}
