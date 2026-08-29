import { useState } from "react";
import { useNavigate } from "react-router-dom";
import { useApi } from "@shared/api/ApiContext";
import { PageHeader } from "@shared/components/PageHeader";
import { ConfirmationDialog } from "@shared/components/ConfirmationDialog";
import type { CatalogScanPreview } from "@shared/api/contracts";

/** This flow must feel safe and non-destructive at every step (§39). */
export function CatalogRecoveryPage({ readOnly }: { readOnly: boolean }) {
  const api = useApi();
  const navigate = useNavigate();
  const [scanning, setScanning] = useState(false);
  const [preview, setPreview] = useState<CatalogScanPreview | null>(null);
  const [confirming, setConfirming] = useState(false);

  const scan = () => {
    setScanning(true);
    api
      .scanCatalog()
      .then(setPreview)
      .finally(() => setScanning(false));
  };

  return (
    <>
      <PageHeader
        back={{ label: "Settings", onClick: () => navigate("/settings") }}
        title="Catalog recovery"
        subtitle="Rebuild the Backup Manager catalog from artifacts already on NAS storage"
      />

      <section className="card">
        <div className="card__body" style={{ display: "flex", flexDirection: "column", gap: 14 }}>
          <div style={{ fontSize: 15, fontWeight: 600 }}>Existing backup data detected</div>
          <p style={{ margin: 0, fontSize: 13, color: "var(--text-2)", maxWidth: "74ch" }}>
            Backup files were found in the configured storage location, but they are not
            currently present in the Backup Manager catalog. Scanning reads file
            metadata and checksums only.
          </p>
          <div className="banner banner--ok" style={{ fontSize: "var(--text-sm)" }}>
            <span aria-hidden="true" style={{ color: "var(--ok)" }}>\u2713</span>
            <span>No files will be deleted, moved, or modified by a scan or a rebuild.</span>
          </div>
          <div>
            <button className="btn btn--primary" disabled={readOnly || scanning} onClick={scan}>
              {scanning ? "Scanning backup storage…" : "Scan backup storage"}
            </button>
          </div>
        </div>
      </section>

      {preview ? (
        <section className="card">
          <div className="card__header"><h2 className="eyebrow">Catalog rebuild preview</h2></div>
          <div className="card__body" style={{ display: "flex", flexDirection: "column", gap: 14 }}>
            <dl style={{ margin: 0, display: "grid", gridTemplateColumns: "1fr auto", gap: "9px 14px", fontSize: 13 }}>
              <dt style={{ color: "var(--text-2)" }}>Backup artifacts discovered</dt>
              <dd className="mono" style={{ margin: 0 }}>{preview.discovered}</dd>
              <dt style={{ color: "var(--text-2)" }}>Valid</dt>
              <dd className="mono" style={{ margin: 0, color: "var(--ok)" }}>{preview.valid}</dd>
              <dt style={{ color: "var(--text-2)" }}>Require review</dt>
              <dd className="mono" style={{ margin: 0, color: "var(--warn)" }}>{preview.requiresReview}</dd>
            </dl>
            <div className="banner banner--info" style={{ fontSize: "var(--text-sm)", color: "var(--text-2)" }}>
              <span aria-hidden="true" style={{ color: "var(--text-3)" }}>i</span>
              <span>Artifacts requiring review are placed in Quarantine, not deleted.</span>
            </div>
            <div style={{ display: "flex", gap: 9 }}>
              <button className="btn" onClick={() => setPreview(null)}>Cancel</button>
              <button className="btn btn--primary" disabled={readOnly} onClick={() => setConfirming(true)}>
                Rebuild catalog
              </button>
            </div>
          </div>
        </section>
      ) : null}

      <ConfirmationDialog
        open={confirming}
        title="Rebuild catalog"
        confirmLabel={"Rebuild from " + (preview?.discovered ?? 0) + " artifacts"}
        onCancel={() => setConfirming(false)}
        onConfirm={() =>
          api.rebuildCatalog().then(() => {
            setConfirming(false);
            navigate("/backups");
          })
        }
      >
        <p style={{ margin: 0 }}>
          The catalog will be rebuilt from the artifacts found on NAS storage.
        </p>
        <p style={{ margin: 0, color: "var(--text-2)" }}>
          This operation is additive. No backup files are deleted, and no remote
          servers are contacted.
        </p>
      </ConfirmationDialog>
    </>
  );
}
