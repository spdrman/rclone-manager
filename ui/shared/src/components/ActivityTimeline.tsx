/**
 * The event log, in the two densities the app reads it at.
 *
 * Dense is the dashboard's recent-activity panel, where the day is already
 * implied by "recent" and only the time of day carries information; the
 * full form is the Activity page, where an event's set has to be named
 * because the list spans all of them. That is the whole difference, and it
 * is one prop rather than two components so the row layout cannot drift
 * apart between the two places it appears.
 *
 * Warnings and errors are bolded as well as coloured and marked with
 * their own icon, which is the third redundant channel on the one list
 * where scanning for the bad line is the actual task.
 */
import type { ActivityEvent, Severity } from "@shared/types/operation";
import { Icon } from "@shared/design-system/icons";
import type { IconName } from "@shared/design-system/icons";
import { stamp } from "@shared/utilities/format";

/** Issue #621 turned this column into artwork, and info is the one row
 *  that changed shape rather than style: it was a middle dot, which is the
 *  mark this app uses to SEPARATE things, sitting in a column whose whole
 *  job is to say what kind of thing a line is. Every other severity had a
 *  picture and info had a punctuation mark standing in for one. */
export const SEVERITY: Record<Severity, { icon: IconName; color: string }> = {
  ok: { icon: "success", color: "var(--ok)" },
  info: { icon: "info", color: "var(--text-3)" },
  warn: { icon: "warning", color: "var(--warn)" },
  error: { icon: "failure", color: "var(--danger)" }
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
              <Icon name={sev.icon} />
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
