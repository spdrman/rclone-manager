import { useNavigate, useParams } from "react-router-dom";
import { useApi } from "@shared/api/ApiContext";
import { useResource } from "@shared/state/resource";
import { artifactDetailNode } from "@shared/state/appNodes";
import { PageHeader } from "@shared/components/PageHeader";
import { StatusBadge } from "@shared/components/StatusBadge";
import { RetentionBadges } from "@shared/components/RetentionBadge";
import { LifecycleTimeline } from "@shared/components/LifecycleTimeline";
import { ErrorState } from "@shared/components/EmptyState";
import { bytes, stamp } from "@shared/utilities/format";

export function BackupDetailPage() {
  const { artifactId = "" } = useParams();
  const api = useApi();
  const navigate = useNavigate();
  // Graph-backed the same way App.tsx's four resources are (B2.4): nothing
  // else reads this particular artifact today, but this keeps every fetched
  // resource on the one mechanism instead of leaving this page on
  // page-local useAsync state.
  const artifact = useResource(artifactDetailNode, () => api.getArtifact(artifactId), [api, artifactId]);

  if (artifact.error) return <ErrorState {...artifact.error} onRetry={artifact.reload} />;
  if (!artifact.data) return null;

  const a = artifact.data;

  return (
    <>
      <PageHeader
        back={{ label: "Backups", onClick: () => navigate("/backups") }}
        title={
          <span style={{ display: "inline-flex", alignItems: "center", gap: 12, flexWrap: "wrap" }}>
            <span className="mono" style={{ fontSize: 19 }}>{a.filename}</span>
            <StatusBadge
              tone={a.validation === "verified" ? "ok" : "danger"}
              glyph={a.validation === "verified" ? "\u2713" : "\u2715"}
            >
              {a.validation === "verified" ? "Verified" : "Failed"}
            </StatusBadge>
            <RetentionBadges classes={a.retentionClasses} />
          </span>
        }
      />

      <div style={{ display: "grid", gridTemplateColumns: "minmax(0, 1fr) minmax(0, 1fr)", gap: 14, alignItems: "start" }}>
        <section className="card">
          <div className="card__header"><h2 className="eyebrow">Artifact</h2></div>
          <dl
            style={{
              margin: 0, padding: "15px 18px", display: "grid",
              gridTemplateColumns: "150px 1fr", gap: "11px 14px", fontSize: "var(--text-sm)"
            }}
          >
            <Row label="Artifact ID" value={a.id} mono />
            <Row label="Backup set" value={a.setName} />
            <Row label="Remote original" value={a.remoteOriginalPath} mono />
            <Row label="Local path" value={a.localPath} mono />
            <Row label="Producer timestamp" value={stamp(a.producedAt)} mono />
            <Row label="Received timestamp" value={stamp(a.receivedAt)} mono />
            <Row label="Size" value={bytes(a.sizeBytes) + " \u00b7 " + a.sizeBytes + " B"} mono />
            <Row label="Checksum" value={a.checksumAlgorithm + ":" + a.checksum} mono />
            <Row
              label="Validation result"
              value={a.validation === "verified" ? "Checksum passed" : "Failed — see Quarantine"}
            />
            <Row label="Retention classes" value={a.retentionClasses.join(", ") || "unclassified"} />
            <Row
              label="Remote source removed"
              value={a.remoteSourceRemovedAt ? stamp(a.remoteSourceRemovedAt) + " (after commit)" : "No — original retained"}
            />
          </dl>
        </section>

        <section className="card">
          <div className="card__header"><h2 className="eyebrow">Lifecycle</h2></div>
          <LifecycleTimeline artifact={a} />
          <p style={{ margin: 0, padding: "0 22px 20px", fontSize: "var(--text-sm)", color: "var(--text-3)", maxWidth: "60ch" }}>
            Remote deletion is a lifecycle consequence of a proven NAS copy — never
            an independent file operation.
          </p>
        </section>
      </div>
    </>
  );
}

function Row({ label, value, mono }: { label: string; value: string; mono?: boolean }) {
  return (
    <>
      <dt style={{ color: "var(--text-2)" }}>{label}</dt>
      <dd
        className={mono ? "mono" : undefined}
        style={{ margin: 0, wordBreak: "break-all" }}
      >
        {value}
      </dd>
    </>
  );
}
