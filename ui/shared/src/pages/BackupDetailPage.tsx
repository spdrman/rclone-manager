import { useNavigate, useParams } from "react-router-dom";
import { useApi } from "@shared/api/ApiContext";
import { useAsync } from "@shared/hooks/useAsync";
import { PageHeader } from "@shared/components/PageHeader";
import { StatusBadge } from "@shared/components/StatusBadge";
import { RetentionBadges } from "@shared/components/RetentionBadge";
import { LifecycleTimeline } from "@shared/components/LifecycleTimeline";
import { PlacementList } from "@shared/components/PlacementList";
import { ErrorState } from "@shared/components/EmptyState";
import { bytes, stamp } from "@shared/utilities/format";

export function BackupDetailPage() {
  const { artifactId = "" } = useParams();
  const api = useApi();
  const navigate = useNavigate();
  // Page-local, like the sibling BackupSetDetailPage (mandatory review on
  // #144): nothing else reads this particular artifact, so there is no
  // duplicate fetch to eliminate by putting it on the shared graph, and
  // going through App.tsx's app-wide resource mechanism actively hurt here
  // — this "resource" changes identity on every navigation to a different
  // :artifactId, so the loading transition that correctly preserves stale
  // data for a genuinely singleton resource instead let one artifact's
  // fields render under a different artifact's URL while the new fetch was
  // in flight.
  const artifact = useAsync(() => api.getArtifact(artifactId), [api, artifactId]);
  // The verification ladder and the retrieval disclosure, in the backend's
  // own words (GET /settings). Fetched here rather than transcribed into
  // PlacementList, because those sentences are what an operator reads
  // while deciding whether a backup is safe, and a paraphrase kept in a
  // frontend is a paraphrase that eventually says something the engine
  // does not. It is deliberately NOT gated on: if it fails, the copies
  // still render and the explanatory sentences are simply absent, which
  // is a worse page and not a wrong one.
  //
  // Page-local rather than a shared graph node, matching the retention
  // card, which fetches the same document the same way. Settings IS a
  // singleton and would eventually belong on the graph, but putting it
  // there means App.tsx owning a fetch and a poll for it, and this page
  // wants the answer once, at open, and does not care if it goes stale
  // while somebody reads one backup's detail. When a third reader
  // appears, that is the change to make.
  const settings = useAsync(() => api.getSettings(), [api]);

  if (artifact.error) return <ErrorState {...artifact.error} onRetry={artifact.reload} />;
  // Both checks matter: `data` is null only before the first successful
  // fetch, but navigating list -> artifact A -> back -> artifact B (or any
  // browser back/forward between two previously visited artifact URLs)
  // keeps this component mounted and re-runs this same useAsync with a new
  // artifactId, so `loading` flips back to true while `data` still holds
  // the PREVIOUS artifact until the new fetch resolves. Gating on loading
  // too closes that window instead of rendering stale fields under the new
  // URL.
  if (!artifact.data || artifact.loading) return null;

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
            {/* The ingestion landing path, labelled as what it is. It is not
                evidence that a readable file is sitting there, and the Copies
                card below is what answers "where are the bytes". */}
            <Row label="Ingestion path" value={a.localPath} mono />
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

      <div style={{ marginTop: 14 }}>
        <PlacementList placements={a.placements} storage={settings.data?.schema.storage} />
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
