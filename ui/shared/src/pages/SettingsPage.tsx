import { useState } from "react";
import { useNavigate } from "react-router-dom";
import { useApi } from "@shared/api/ApiContext";
import { useAsync } from "@shared/hooks/useAsync";
import { usePlatform } from "@shared/platform/PlatformContext";
import { notificationCopy } from "@shared/platform/capabilities";
import { PageHeader } from "@shared/components/PageHeader";
import { PlatformBadge } from "@shared/components/PlatformBadge";
import { ErrorState } from "@shared/components/EmptyState";

export function SettingsPage({ readOnly }: { readOnly: boolean }) {
  const api = useApi();
  const navigate = useNavigate();
  const { bridge, capabilityCopy } = usePlatform();
  const version = useAsync(() => api.getVersion(), [api]);

  return (
    <>
      <PageHeader
        title="Settings"
        subtitle="Service behaviour, platform integration and build information"
      />

      <div style={{ display: "grid", gridTemplateColumns: "minmax(0, 1.3fr) minmax(0, 1fr)", gap: 14, alignItems: "start" }}>
        <div style={{ display: "flex", flexDirection: "column", gap: 14 }}>
          <section className="card">
            <div className="card__header"><h2 className="eyebrow">Service</h2></div>
            <div
              className="card__body"
              style={{ display: "grid", gridTemplateColumns: "repeat(auto-fit, minmax(196px, 1fr))", gap: "15px 18px" }}
            >
              <label className="field">
                <span className="field__label">Polling interval</span>
                <select className="select" defaultValue="30" disabled={readOnly}>
                  <option value="15">15 seconds</option>
                  <option value="30">30 seconds</option>
                  <option value="60">60 seconds</option>
                </select>
              </label>
              <label className="field">
                <span className="field__label">Log level</span>
                <select className="select" defaultValue="info" disabled={readOnly}>
                  <option>error</option><option>warn</option><option>info</option><option>debug</option>
                </select>
              </label>
              <label className="field">
                <span className="field__label">Storage warning threshold</span>
                <input className="input input--mono" defaultValue="80%" disabled={readOnly} />
              </label>
              <label className="field">
                <span className="field__label">Storage critical threshold</span>
                <input className="input input--mono" defaultValue="92%" disabled={readOnly} />
              </label>
              <div style={{ gridColumn: "1 / -1", display: "flex", flexDirection: "column", gap: 7 }}>
                <span className="field__label">Default retention for new sets</span>
                <div className="mono" style={{ display: "flex", gap: 9, flexWrap: "wrap", fontSize: "var(--text-sm)" }}>
                  {["7 daily", "13 weekly", "12 monthly"].map((t) => (
                    <span key={t} style={{ padding: "5px 10px", border: "1px solid var(--border)", borderRadius: "var(--radius-md)", background: "var(--surface-2)" }}>
                      {t}
                    </span>
                  ))}
                  <span style={{ padding: "5px 10px", border: "1px solid var(--ok)", borderRadius: "var(--radius-md)", background: "var(--ok-quiet)" }}>
                    protect known-good
                  </span>
                </div>
              </div>
            </div>
          </section>

          <section className="card">
            <div className="card__header"><h2 className="eyebrow">Notifications</h2></div>
            <div className="card__body" style={{ display: "flex", flexDirection: "column", gap: 12 }}>
              {/* Honest capability copy — never present a fallback as native (§22). */}
              <div className="banner banner--info" style={{ fontSize: "var(--text-sm)", color: "var(--text-2)" }}>
                <span aria-hidden="true" style={{ color: "var(--text-3)" }}>i</span>
                <span>{notificationCopy(bridge.capabilities(), bridge.name)}</span>
              </div>
              <label style={{ display: "flex", alignItems: "center", gap: 10, padding: "11px 13px", border: "1px solid var(--border)", borderRadius: 7, fontSize: 13, cursor: "pointer" }}>
                <input type="checkbox" defaultChecked disabled={readOnly} style={{ accentColor: "var(--accent)" }} />
                <span style={{ flex: 1 }}>Webhook notifications</span>
                <span className="mono" style={{ fontSize: "var(--text-sm)", color: "var(--text-3)" }}>
                  https://hooks.internal/bm
                </span>
              </label>
            </div>
          </section>

          <ChangePasswordCard readOnly={readOnly} />

          <section className="card" style={{ borderColor: "var(--warn)" }}>
            <div className="card__header"><h2 className="eyebrow">Catalog recovery</h2></div>
            <div className="card__body" style={{ display: "flex", flexDirection: "column", gap: 12 }}>
              <div style={{ fontSize: 13.5, fontWeight: 600 }}>Existing backup data detected</div>
              <p style={{ margin: 0, fontSize: 13, color: "var(--text-2)", maxWidth: "74ch" }}>
                Backup files were found in the configured storage location, but they are
                not currently present in the Backup Manager catalog. Scanning is
                read-only — no files will be deleted.
              </p>
              <div>
                <button className="btn btn--primary" disabled={readOnly} onClick={() => navigate("/catalog-recovery")}>
                  Scan backup storage
                </button>
              </div>
            </div>
          </section>
        </div>

        <div style={{ display: "flex", flexDirection: "column", gap: 14 }}>
          <section className="card">
            <div className="card__header"><h2 className="eyebrow">Platform</h2></div>
            <div className="card__body">
              <PlatformBadge />
              <div className="eyebrow" style={{ fontSize: 10.5, margin: "16px 0 8px" }}>Capabilities</div>
              <div style={{ display: "flex", flexDirection: "column", gap: 6 }}>
                {capabilityCopy.map((c) => (
                  <div key={c.label} style={{ display: "flex", alignItems: "center", gap: 9, fontSize: "var(--text-sm)" }}>
                    <span
                      aria-hidden="true"
                      style={{ width: 12, textAlign: "center", color: c.supported ? "var(--ok)" : "var(--text-3)" }}
                    >
                      {c.supported ? "\u2713" : "\u2013"}
                    </span>
                    <span style={{ flex: 1 }}>{c.label}</span>
                    <span className="mono" style={{ fontSize: "var(--text-xs)", color: "var(--text-3)" }}>
                      {c.detail}
                    </span>
                  </div>
                ))}
              </div>
            </div>
          </section>

          <section className="card">
            <div className="card__header"><h2 className="eyebrow">System information</h2></div>
            <div className="card__body">
              {version.data ? (
                <dl style={{ margin: 0, display: "grid", gridTemplateColumns: "1fr auto", gap: "10px 14px", fontSize: "var(--text-sm)" }}>
                  <Row label="Backup Manager version" value={version.data.ui} />
                  <Row label="Service version" value={version.data.service} />
                  <Row label="Core version" value={version.data.core} />
                  <Row label="Embedded rclone" value={version.data.rclone} />
                  <Row label="Database schema" value={String(version.data.schema)} />
                  <Row label="Platform adapter" value={bridge.deployment.adapterVersion} />
                  <Row label="Architecture" value={version.data.architecture} />
                  <Row label="Build commit" value={version.data.buildCommit} />
                </dl>
              ) : null}
            </div>
          </section>
        </div>
      </div>
    </>
  );
}

function Row({ label, value }: { label: string; value: string }) {
  return (
    <>
      <dt style={{ color: "var(--text-2)" }}>{label}</dt>
      <dd className="mono" style={{ margin: 0 }}>{value}</dd>
    </>
  );
}

const MIN_PASSWORD_LENGTH = 12;

/** §13A password rotation (issue #128) - reuses EnrollmentPage.tsx's own
 *  validation shape (minimum length, confirm-match) since it is the same
 *  "pick a new password" moment, just for an existing account instead of
 *  a first-run one. A successful rotation signs out every OTHER session
 *  for this administrator (apps/common/auth/local's handleRotatePassword);
 *  this tab's own session is reissued, so no redirect/sign-out happens
 *  here. */
function ChangePasswordCard({ readOnly }: { readOnly: boolean }) {
  const api = useApi();
  const [current, setCurrent] = useState("");
  const [next, setNext] = useState("");
  const [confirm, setConfirm] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [success, setSuccess] = useState(false);

  const tooShort = next.length > 0 && next.length < MIN_PASSWORD_LENGTH;
  const mismatch = confirm.length > 0 && confirm !== next;
  const valid = current.length > 0 && next.length >= MIN_PASSWORD_LENGTH && confirm === next;

  const submit = (e: React.FormEvent) => {
    e.preventDefault();
    if (!valid || readOnly) return;
    setBusy(true);
    setError(null);
    setSuccess(false);
    api
      .rotatePassword(current, next)
      .then(() => {
        setSuccess(true);
        setCurrent("");
        setNext("");
        setConfirm("");
      })
      .catch(() => setError("That current password was not accepted."))
      .finally(() => setBusy(false));
  };

  return (
    <section className="card">
      <div className="card__header"><h2 className="eyebrow">Administrator password</h2></div>
      <div className="card__body">
        <form onSubmit={submit} style={{ display: "flex", flexDirection: "column", gap: 14 }}>
          <label className="field">
            <span className="field__label">Current password</span>
            <input
              className="input"
              type="password"
              autoComplete="current-password"
              value={current}
              onChange={(e) => setCurrent(e.target.value)}
              disabled={readOnly}
              required
            />
          </label>
          <label className="field">
            <span className="field__label">New password</span>
            <input
              className="input"
              type="password"
              autoComplete="new-password"
              value={next}
              onChange={(e) => setNext(e.target.value)}
              disabled={readOnly}
              required
            />
            {tooShort ? (
              <span style={{ fontSize: "var(--text-sm)", color: "var(--danger)" }}>
                {"Minimum " + MIN_PASSWORD_LENGTH + " characters."}
              </span>
            ) : null}
          </label>
          <label className="field">
            <span className="field__label">Confirm new password</span>
            <input
              className="input"
              type="password"
              autoComplete="new-password"
              value={confirm}
              onChange={(e) => setConfirm(e.target.value)}
              disabled={readOnly}
              required
            />
            {mismatch ? (
              <span style={{ fontSize: "var(--text-sm)", color: "var(--danger)" }}>
                Passwords do not match.
              </span>
            ) : null}
          </label>
          {success ? (
            <div className="banner banner--ok" style={{ fontSize: "var(--text-sm)" }}>
              Password changed. Other signed-in sessions have been signed out.
            </div>
          ) : null}
          {error ? <ErrorState message={error} correlationId="cid_rotate_password" /> : null}
          <div>
            <button
              className="btn btn--primary"
              type="submit"
              disabled={!valid || busy || readOnly}
              style={{ height: 40 }}
            >
              {busy ? "Changing…" : "Change password"}
            </button>
          </div>
        </form>
      </div>
    </section>
  );
}
