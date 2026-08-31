import type { SystemHealth } from "@shared/types/operation";
import { HEALTH_PRESENTATION, StatusBadge } from "./StatusBadge";
import { bytes, relativeAge } from "@shared/utilities/format";

/** §8: "APPLICATION RUNNING" and "BACKUPS HEALTHY" are two different facts.
 *  The headline states the BACKUP verdict; the daemon is a supporting chip. */
export function HealthSummary({ health }: { health: SystemHealth }) {
  const p = HEALTH_PRESENTATION[health.backupHealth];
  const headline = "BACKUPS " + p.label.toUpperCase();
  const color =
    p.tone === "ok" ? "var(--ok)" : p.tone === "warn" ? "var(--warn)" : "var(--danger)";

  return (
    <section className="card" aria-label="Backup health">
      <div
        style={{
          display: "flex", alignItems: "flex-start", gap: 20, flexWrap: "wrap",
          padding: "20px 22px", borderBottom: "1px solid var(--border)"
        }}
      >
        <div style={{ flex: 1, minWidth: 300, display: "flex", flexDirection: "column", gap: 9 }}>
          <div style={{ display: "flex", alignItems: "center", gap: 10 }}>
            <span aria-hidden="true" style={{ color, fontSize: 15 }}>{p.glyph}</span>
            <span
              style={{
                fontFamily: "var(--font-mono)", fontSize: 17, fontWeight: 600,
                letterSpacing: "0.04em", color
              }}
            >
              {headline}
            </span>
          </div>
          <p style={{ margin: 0, fontSize: 13.5, color: "var(--text-2)", maxWidth: "66ch" }}>
            {health.backupHealthReason}
          </p>
          <div style={{ display: "flex", gap: 8, flexWrap: "wrap", marginTop: 3 }}>
            {/* "Service running" and nothing more. The service answered
                this request, which is the whole claim; it used to carry an
                uptime figure that no endpoint has ever reported (#211). */}
            <StatusBadge tone={health.serviceRunning ? "ok" : "danger"} glyph={health.serviceRunning ? "\u25cf" : "\u2715"}>
              {health.serviceRunning ? "Service running" : "Service stopped"}
            </StatusBadge>
            <StatusBadge
              tone={health.storageState === "nominal" ? "ok" : health.storageState === "warning" ? "warn" : "danger"}
              glyph={health.storageState === "nominal" ? "\u25cf" : "\u25b2"}
            >
              {"Storage " + health.storageState}
            </StatusBadge>
            {health.setsStale > 0 ? (
              <StatusBadge tone="warn" glyph="\u25b2">{health.setsStale + " set stale"}</StatusBadge>
            ) : null}
            {health.setsDegraded > 0 ? (
              <StatusBadge tone="warn" glyph="\u25b2">{health.setsDegraded + " set degraded"}</StatusBadge>
            ) : null}
            {health.setsFailing > 0 ? (
              <StatusBadge tone="danger" glyph="\u2715">{health.setsFailing + " set halted"}</StatusBadge>
            ) : null}
          </div>
        </div>

        <dl
          style={{
            flex: "none", margin: 0, display: "grid",
            gridTemplateColumns: "auto auto", gap: "9px 26px", fontSize: 13
          }}
        >
          <Row label="Last completed backup" value={relativeAge(health.lastCompletedBackupAt)} />
          <Row label="Newest verified backup" value={relativeAge(health.newestVerifiedBackupAt)} />
          {/* Null is "some set has never produced a known-good backup", not
              a large number of hours. Rendering a figure there would be
              false precision an operator could act on. */}
          <Row
            label="Oldest set freshness"
            value={
              health.oldestSetFreshnessHours === null
                ? "not yet measurable"
                : health.oldestSetFreshnessHours + " hours ago"
            }
            tone={
              health.oldestSetFreshnessHours !== null && health.oldestSetFreshnessHours > 24
                ? "var(--warn)"
                : undefined
            }
          />
          <Row label="Free space" value={bytes(health.storageFreeBytes)} />
          {health.storageReadingsUnavailable > 0 ? (
            <Row
              label="Capacity unreadable"
              value={health.storageReadingsUnavailable + " destination(s)"}
              tone="var(--warn)"
            />
          ) : null}
        </dl>
      </div>
    </section>
  );
}

function Row({ label, value, tone }: { label: string; value: string; tone?: string }) {
  return (
    <>
      <dt style={{ color: "var(--text-2)" }}>{label}</dt>
      <dd style={{ margin: 0, fontFamily: "var(--font-mono)", textAlign: "right", color: tone }}>
        {value}
      </dd>
    </>
  );
}
