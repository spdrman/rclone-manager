import { useState } from "react";
import { useNavigate } from "react-router-dom";
import { useApi } from "@shared/api/ApiContext";
import { useAsync } from "@shared/hooks/useAsync";
import { PageHeader } from "@shared/components/PageHeader";
import { RetentionBadges } from "@shared/components/RetentionBadge";
import { EmptyState, ErrorState } from "@shared/components/EmptyState";
import { RetentionPreviewDialog } from "./RetentionPreviewDialog";
import { bytes, stamp } from "@shared/utilities/format";

/** Called "Backups", never "Restore points" — the product does not perform
 *  application restore (§13). */
export function BackupsPage({ readOnly }: { readOnly: boolean }) {
  const api = useApi();
  const navigate = useNavigate();
  const [setFilter, setSetFilter] = useState("");
  const [previewFor, setPreviewFor] = useState<string | null>(null);

  const sets = useAsync(() => api.listSets(), [api]);
  const artifacts = useAsync(() => api.listArtifacts(setFilter || undefined), [api, setFilter]);

  if (artifacts.error) return <ErrorState {...artifacts.error} onRetry={artifacts.reload} />;

  const rows = artifacts.data ?? [];
  const totalBytes = rows.reduce((n, a) => n + a.sizeBytes, 0);

  return (
    <>
      <PageHeader
        title="Backups"
        subtitle={rows.length + " retained artifacts \u00b7 " + bytes(totalBytes)}
        actions={
          <>
            <select
              className="select"
              style={{ height: 32 }}
              aria-label="Filter by backup set"
              value={setFilter}
              onChange={(e) => setSetFilter(e.target.value)}
            >
              <option value="">All backup sets</option>
              {(sets.data ?? []).map((s) => (
                <option key={s.id} value={s.id}>{s.name}</option>
              ))}
            </select>
            <button
              className="btn"
              disabled={readOnly || !setFilter}
              title={setFilter ? undefined : "Choose a backup set to preview its retention plan"}
              onClick={() => setPreviewFor(setFilter)}
            >
              Preview retention
            </button>
          </>
        }
      />

      {rows.length === 0 ? (
        <EmptyState title="No backups yet">
          This backup set has not completed its first successful ingestion.
        </EmptyState>
      ) : (
        <div className="card">
          {/* A small app window must never hide a column (§31). */}
          <div className="table-scroll">
            <table className="table" style={{ minWidth: 820 }}>
              <caption className="eyebrow" style={{ textAlign: "left", padding: "12px 16px", borderBottom: "1px solid var(--border)" }}>
                Retained backups
              </caption>
              <thead>
                <tr>
                  <th scope="col">Time</th>
                  <th scope="col">Backup set</th>
                  <th scope="col">Artifact</th>
                  <th scope="col" style={{ textAlign: "right" }}>Size</th>
                  <th scope="col">Validation</th>
                  <th scope="col">Retention</th>
                  <th scope="col">Status</th>
                </tr>
              </thead>
              <tbody>
                {rows.map((a) => (
                  <tr
                    key={a.id}
                    onClick={() => navigate("/backups/" + a.id)}
                    style={{ cursor: "pointer" }}
                  >
                    <td className="mono" style={{ whiteSpace: "nowrap", color: "var(--text-2)" }}>
                      {stamp(a.receivedAt)}
                    </td>
                    <td>{a.setName}</td>
                    <td className="mono" style={{ fontSize: "var(--text-sm)" }}>{a.filename}</td>
                    <td className="mono" style={{ textAlign: "right", whiteSpace: "nowrap" }}>{bytes(a.sizeBytes)}</td>
                    <td style={{ whiteSpace: "nowrap" }}>
                      <span
                        style={{
                          display: "inline-flex", alignItems: "center", gap: 6,
                          fontSize: "var(--text-sm)",
                          color: a.validation === "verified" ? "var(--text)" : "var(--danger)"
                        }}
                      >
                        <span aria-hidden="true" style={{ color: a.validation === "verified" ? "var(--ok)" : "var(--danger)" }}>
                          {a.validation === "verified" ? "\u2713" : "\u25b2"}
                        </span>
                        {a.validation === "verified" ? "Verified" : a.validation === "failed" ? "Failed" : "Pending"}
                      </span>
                    </td>
                    <td><RetentionBadges classes={a.retentionClasses} /></td>
                    <td style={{ fontSize: "var(--text-sm)", color: "var(--text-2)", whiteSpace: "nowrap" }}>
                      {a.remoteSourceRemovedAt ? "Remote source removed" : "Remote source retained"}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
          <div
            className="card__footer"
            style={{ display: "flex", justifyContent: "space-between", gap: 16, fontSize: "var(--text-sm)", color: "var(--text-2)" }}
          >
            <span>{"Showing " + rows.length + " artifacts"}</span>
            <span style={{ color: "var(--text-3)" }}>
              Backup Manager does not perform application restore — these are
              retained, verified copies.
            </span>
          </div>
        </div>
      )}

      {previewFor ? (
        <RetentionPreviewDialog setId={previewFor} open onClose={() => setPreviewFor(null)} />
      ) : null}
    </>
  );
}
