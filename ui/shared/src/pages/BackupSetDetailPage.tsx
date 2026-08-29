import { useState } from "react";
import { useNavigate, useParams } from "react-router-dom";
import { useApi } from "@shared/api/ApiContext";
import { useAsync } from "@shared/hooks/useAsync";
import { PageHeader } from "@shared/components/PageHeader";
import { HealthBadge } from "@shared/components/StatusBadge";
import { FingerprintDisplay } from "@shared/components/FingerprintDisplay";
import { ActivityTimeline } from "@shared/components/ActivityTimeline";
import { WarningBanner } from "@shared/components/WarningBanner";
import { ConfirmationDialog } from "@shared/components/ConfirmationDialog";
import { ErrorState } from "@shared/components/EmptyState";
import { RetentionPreviewDialog } from "./RetentionPreviewDialog";
import { bytes, relativeAge } from "@shared/utilities/format";

const COMPLETION_COPY = {
  "atomic-rename": ["Atomic rename", "Producer writes to a temporary name, then renames into place."],
  "completion-marker": ["Completion marker / manifest", "Producer writes a sidecar manifest when the artifact is complete."],
  "stable-size": ["Stable file size / timestamp", "Infers completion \u2014 less assurance than a producer-provided marker."]
} as const;

export function BackupSetDetailPage({ readOnly }: { readOnly: boolean }) {
  const { setId = "" } = useParams();
  const api = useApi();
  const navigate = useNavigate();
  const set = useAsync(() => api.getSet(setId), [api, setId]);
  const activity = useAsync(() => api.listActivity(), [api]);
  const [previewOpen, setPreviewOpen] = useState(false);
  const [removeOpen, setRemoveOpen] = useState(false);

  if (set.error) return <ErrorState {...set.error} onRetry={set.reload} />;
  if (!set.data) return null;

  const s = set.data;
  const [methodLabel, methodDetail] = COMPLETION_COPY[s.completionMethod];
  const events = (activity.data ?? []).filter((e) => e.setId === s.id).slice(0, 6);

  return (
    <>
      <PageHeader
        back={{ label: "Backup sets", onClick: () => navigate("/sets") }}
        title={
          <span style={{ display: "inline-flex", alignItems: "center", gap: 11 }}>
            {s.name}
            <HealthBadge state={s.state} />
          </span>
        }
        subtitle={
          <span className="mono">
            {s.host + ":" + s.port + " \u00b7 " + s.remoteFolder}
          </span>
        }
        actions={
          <>
            <button className="btn btn--primary" disabled={readOnly || s.halted} onClick={() => api.runSet(s.id).then(set.reload)}>
              Run now
            </button>
            <button className="btn" disabled={readOnly} onClick={() => api.testConnection(s.id)}>Test connection</button>
            <button className="btn" disabled={readOnly}>Edit</button>
            <button className="btn" disabled={readOnly} onClick={() => setPreviewOpen(true)}>Preview retention</button>
          </>
        }
      />

      {s.haltReason === "host-key-changed" ? (
        <WarningBanner
          tone="danger"
          eyebrow="Security warning"
          title={"The SSH host key for " + s.host + " has changed"}
          actions={
            <>
              <button className="btn btn--sm" disabled={readOnly}>Compare fingerprints</button>
              <button className="btn btn--sm">Keep set halted</button>
            </>
          }
        >
          Backup operations have been stopped until the new fingerprint is
          independently verified. No remote artifacts will be deleted while this
          set is halted.
        </WarningBanner>
      ) : null}

      <div
        style={{
          display: "grid", gridTemplateColumns: "minmax(0, 1.55fr) minmax(0, 1fr)",
          gap: 14, alignItems: "start"
        }}
      >
        <div style={{ display: "flex", flexDirection: "column", gap: 14, minWidth: 0 }}>
          <Section title="Overview">
            <dl
              style={{
                margin: 0, display: "grid",
                gridTemplateColumns: "repeat(auto-fit, minmax(150px, 1fr))",
                gap: "15px 18px", fontSize: 13
              }}
            >
              <Cell label="Newest known-good" value={relativeAge(s.newestKnownGoodAt)} mono />
              <Cell label="Last successful run" value={relativeAge(s.lastRunAt)} mono />
              <Cell label="Retained" value={s.retainedCount + " \u00b7 " + bytes(s.retainedBytes)} mono />
              <Cell label="Expected cadence" value={"every " + s.expectedIntervalHours + "h"} mono />
              <Cell label="State" value={s.stateNote} />
              <Cell label="Remote cleanup" value={s.enabled ? "Enabled after commit" : "Disabled"} />
            </dl>
          </Section>

          <Section title="Connection">
            <FingerprintDisplay
              host={s.host}
              algorithm="ssh-ed25519"
              fingerprint={s.hostFingerprint}
              trustedAt={s.fingerprintTrustedAt}
            />
            <p style={{ margin: "12px 0 0", fontSize: "var(--text-sm)", color: "var(--text-3)" }}>
              The private key never leaves this NAS and is never displayed.
            </p>
          </Section>

          <Section title="Backup discovery">
            <dl style={{ margin: 0, display: "grid", gridTemplateColumns: "172px 1fr", gap: "11px 16px", fontSize: 13 }}>
              <dt style={{ color: "var(--text-2)" }}>Remote folder</dt>
              <dd className="mono" style={{ margin: 0 }}>{s.remoteFolder}</dd>
              <dt style={{ color: "var(--text-2)" }}>Include</dt>
              <dd className="mono" style={{ margin: 0 }}>{s.includePatterns.join(", ") || "\u2014"}</dd>
              <dt style={{ color: "var(--text-2)" }}>Exclude</dt>
              <dd className="mono" style={{ margin: 0 }}>{s.excludePatterns.join(", ") || "\u2014"}</dd>
              <dt style={{ color: "var(--text-2)" }}>Completion method</dt>
              <dd style={{ margin: 0 }}>
                {methodLabel}
                <span style={{ color: "var(--text-3)" }}>{" \u2014 " + methodDetail}</span>
              </dd>
            </dl>
            {s.completionMethod === "stable-size" ? (
              <div style={{ marginTop: 12 }}>
                <WarningBanner tone="warn">
                  This method infers completion and provides less assurance than a
                  producer-provided completion marker.
                </WarningBanner>
              </div>
            ) : null}
          </Section>

          <Section title="Activity">
            <ActivityTimeline events={events} dense />
          </Section>
        </div>

        <div style={{ display: "flex", flexDirection: "column", gap: 14, minWidth: 0 }}>
          <Section title="Retention">
            <div style={{ display: "flex", flexDirection: "column", gap: 11, fontSize: 13 }}>
              <KV label="Daily" value={s.retention.daily + " kept"} />
              <KV label="Weekly" value={s.retention.weekly + " kept"} />
              <KV label="Monthly" value={s.retention.monthly + " kept"} />
              <KV label="Timezone \u00b7 week start" value={s.retention.timezone + " \u00b7 " + s.retention.weekStartsOn} />
              {s.retention.protectLastKnownGood ? (
                <div className="banner banner--ok" style={{ fontSize: "var(--text-sm)" }}>
                  <span aria-hidden="true" style={{ color: "var(--ok)" }}>\u2713</span>
                  <span>Newest known-good backup is protected from deletion</span>
                </div>
              ) : null}
              <button className="btn btn--caution" disabled={readOnly} onClick={() => setPreviewOpen(true)}>
                Preview retention plan
              </button>
            </div>
          </Section>

          <Section title="Validation">
            <ul style={{ margin: 0, padding: 0, listStyle: "none", display: "flex", flexDirection: "column", gap: 10, fontSize: 13 }}>
              {(["transfer", "checksum", "application"] as const).map((v) => {
                const on = s.validations.includes(v);
                return (
                  <li key={v} style={{ display: "flex", gap: 9 }}>
                    <span aria-hidden="true" style={{ color: on ? "var(--ok)" : "var(--text-3)" }}>
                      {on ? "\u2713" : "\u2013"}
                    </span>
                    <span>
                      {v === "transfer" ? "Transfer verification" : v === "checksum" ? "Checksum verification (SHA-256)" : "Application validation"}
                      {on ? null : <span style={{ color: "var(--text-3)" }}> — not enabled</span>}
                    </span>
                  </li>
                );
              })}
            </ul>
          </Section>

          {/* Caution and destructive actions live apart from ordinary ones (§11, §35). */}
          <Section title="Set management">
            <div style={{ display: "flex", flexDirection: "column", gap: 10 }}>
              <button className="btn btn--caution" disabled={readOnly} onClick={() => api.setEnabled(s.id, !s.enabled).then(set.reload)}>
                {s.enabled ? "Disable backup set" : "Enable backup set"}
              </button>
              <button className="btn btn--destructive" disabled={readOnly} onClick={() => setPreviewOpen(true)}>
                Apply retention now…
              </button>
              <button className="btn btn--destructive" disabled={readOnly} onClick={() => setRemoveOpen(true)}>
                Remove set configuration…
              </button>
              <p style={{ margin: 0, fontSize: "var(--text-sm)", color: "var(--text-3)" }}>
                Removing configuration never deletes retained backups from NAS storage.
              </p>
            </div>
          </Section>
        </div>
      </div>

      <RetentionPreviewDialog setId={s.id} open={previewOpen} onClose={() => setPreviewOpen(false)} />

      <ConfirmationDialog
        open={removeOpen}
        destructive
        eyebrow="Destructive action"
        title="Remove backup set configuration"
        confirmLabel="Remove configuration"
        onCancel={() => setRemoveOpen(false)}
        onConfirm={() => setRemoveOpen(false)}
      >
        <p style={{ margin: 0 }}>
          {"Backup Manager will stop collecting backups for " + s.name + "."}
        </p>
        <p style={{ margin: 0, color: "var(--text-2)" }}>
          {s.retainedCount + " retained backups (" + bytes(s.retainedBytes) + ") stay on NAS storage and remain listed under Backups."}
        </p>
      </ConfirmationDialog>
    </>
  );
}

function Section({ title, children }: { title: string; children: React.ReactNode }) {
  return (
    <section className="card">
      <div className="card__header">
        <h2 className="eyebrow">{title}</h2>
      </div>
      <div className="card__body">{children}</div>
    </section>
  );
}

function Cell({ label, value, mono }: { label: string; value: string; mono?: boolean }) {
  return (
    <div>
      <dt className="eyebrow" style={{ fontSize: 10.5, letterSpacing: "0.06em" }}>{label}</dt>
      <dd style={{ margin: "4px 0 0", fontFamily: mono ? "var(--font-mono)" : undefined }}>{value}</dd>
    </div>
  );
}

function KV({ label, value }: { label: string; value: string }) {
  return (
    <div style={{ display: "flex", justifyContent: "space-between", gap: 12 }}>
      <span style={{ color: "var(--text-2)" }}>{label}</span>
      <span className="mono">{value}</span>
    </div>
  );
}
