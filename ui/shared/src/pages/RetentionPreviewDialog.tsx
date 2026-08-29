import { useState } from "react";
import { useApi } from "@shared/api/ApiContext";
import { useAsync } from "@shared/hooks/useAsync";
import type { RetentionPlan } from "@shared/types/backup";
import { ConfirmationDialog } from "@shared/components/ConfirmationDialog";
import { WarningBanner } from "@shared/components/WarningBanner";
import { RetentionBadges } from "@shared/components/RetentionBadge";
import { bytes } from "@shared/utilities/format";

/** §17 — the plan is server-issued and immutable. If it goes stale we refuse to
 *  apply and never silently recalculate a different deletion set. */
export function RetentionPreviewDialog({
  setId,
  open,
  onClose
}: {
  setId: string;
  open: boolean;
  onClose(): void;
}) {
  const api = useApi();
  const [confirming, setConfirming] = useState(false);
  const plan = useAsync(() => (open ? api.previewRetention(setId) : Promise.resolve(null as never)), [api, setId, open]);

  if (!open) return null;

  const p: RetentionPlan | null = plan.data;

  return (
    <>
      <div className="dialog-scrim" onClick={(e) => e.target === e.currentTarget && onClose()}>
        <div
          role="dialog"
          aria-modal="true"
          aria-label="Retention preview"
          className="dialog"
          style={{ maxWidth: 620, maxHeight: "84vh", overflow: "auto" }}
        >
          <div style={{ padding: "18px 22px", borderBottom: "1px solid var(--border)" }}>
            <h2>Retention preview</h2>
            <p style={{ margin: "4px 0 0", fontSize: "var(--text-sm)", color: "var(--text-2)" }}>
              {p ? "Plan " + p.planId + " \u00b7 issued by the backup service" : "Requesting plan…"}
            </p>
          </div>

          {p?.stale ? (
            <div style={{ padding: "16px 22px 0" }}>
              <WarningBanner
                tone="warn"
                title="Retention preview changed"
                actions={
                  <button className="btn btn--sm" onClick={plan.reload}>Review new plan</button>
                }
              >
                The backup inventory changed after this preview was created. No files
                were deleted. Review the updated retention plan before continuing.
              </WarningBanner>
            </div>
          ) : null}

          {p ? (
            <>
              <div
                style={{
                  margin: "18px 22px 0", display: "grid", gridTemplateColumns: "repeat(3, 1fr)",
                  gap: 1, background: "var(--border)", border: "1px solid var(--border)",
                  borderRadius: "var(--radius-lg)", overflow: "hidden"
                }}
              >
                <Stat label="Keep" value={String(p.keep.length)} />
                <Stat label="Delete" value={String(p.delete.length)} tone="var(--danger)" />
                <Stat label="Reclaim" value={bytes(p.reclaimBytes)} />
              </div>

              <div style={{ padding: "18px 22px", display: "flex", flexDirection: "column", gap: 16 }}>
                <div>
                  <div className="eyebrow" style={{ color: "var(--ok)", marginBottom: 8 }}>Keep</div>
                  <ul style={{ margin: 0, padding: 0, listStyle: "none", display: "flex", flexDirection: "column", gap: 6 }}>
                    {p.keep.map((k) => (
                      <li key={k.artifactId} style={{ display: "flex", justifyContent: "space-between", gap: 12, fontSize: "var(--text-sm)" }}>
                        <span className="mono">{k.date}</span>
                        <RetentionBadges classes={k.classes} />
                      </li>
                    ))}
                  </ul>
                </div>
                <div>
                  <div className="eyebrow" style={{ color: "var(--danger)", marginBottom: 8 }}>Delete</div>
                  <ul style={{ margin: 0, padding: 0, listStyle: "none", display: "flex", flexDirection: "column", gap: 6 }}>
                    {p.delete.map((d) => (
                      <li key={d.artifactId} style={{ display: "flex", justifyContent: "space-between", gap: 12, fontSize: "var(--text-sm)" }}>
                        <span className="mono">{d.date}</span>
                        <span style={{ color: "var(--text-2)" }}>{d.reason}</span>
                      </li>
                    ))}
                  </ul>
                </div>
              </div>
            </>
          ) : null}

          <div className="card__footer" style={{ display: "flex", justifyContent: "flex-end", gap: 9, borderRadius: "0 0 10px 10px" }}>
            <button className="btn" onClick={onClose}>Cancel</button>
            <button
              className="btn btn--destructive"
              disabled={!p || p.stale || p.delete.length === 0}
              onClick={() => setConfirming(true)}
            >
              Continue…
            </button>
          </div>
        </div>
      </div>

      {p ? (
        <ConfirmationDialog
          open={confirming}
          destructive
          eyebrow="Destructive action"
          title="Apply retention"
          confirmLabel={"Delete " + p.delete.length + " backups"}
          onCancel={() => setConfirming(false)}
          onConfirm={() =>
            api.applyRetention(p.planId).then(() => {
              setConfirming(false);
              onClose();
            })
          }
        >
          <p style={{ margin: 0 }}>
            {p.delete.length + " retained backup files will be permanently removed from NAS storage."}
          </p>
          <p style={{ margin: 0, color: "var(--text-2)" }}>
            {bytes(p.reclaimBytes) + " will be reclaimed. This is the exact plan you reviewed \u2014 it will not be recalculated."}
          </p>
          {p.protectedArtifactId ? (
            <div className="banner banner--ok" style={{ fontSize: "var(--text-sm)" }}>
              <span aria-hidden="true" style={{ color: "var(--ok)" }}>\u2713</span>
              <span>The newest known-good backup is protected.</span>
            </div>
          ) : null}
        </ConfirmationDialog>
      ) : null}
    </>
  );
}

function Stat({ label, value, tone }: { label: string; value: string; tone?: string }) {
  return (
    <div style={{ background: "var(--surface)", padding: "13px 15px" }}>
      <div className="eyebrow" style={{ fontSize: 10.5 }}>{label}</div>
      <div style={{ marginTop: 5, fontSize: 20, fontWeight: 600, color: tone }}>{value}</div>
    </div>
  );
}
