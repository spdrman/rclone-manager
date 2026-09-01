import { useState } from "react";
import { useNavigate } from "react-router-dom";
import { useApi } from "@shared/api/ApiContext";
import { usePlatform } from "@shared/platform/PlatformContext";
import { notificationCopy } from "@shared/platform/capabilities";
import { useCausl } from "@shared/state/graph";
import { versionNode } from "@shared/state/appNodes";
import { PageHeader } from "@shared/components/PageHeader";
import { PlatformBadge } from "@shared/components/PlatformBadge";
import { ErrorState } from "@shared/components/EmptyState";
import { apiErrorOf, describeFailure } from "@shared/api/failure";
import type { OperatorFailure } from "@shared/api/failure";
import { RetentionPolicyCard } from "@shared/pages/RetentionPolicyCard";
import { HelpField } from "@shared/components/FieldHelp";
import { FIELD_HELP } from "@shared/components/fieldHelpCopy";

export function SettingsPage({ readOnly }: { readOnly: boolean }) {
  const navigate = useNavigate();
  const { bridge, capabilityCopy } = usePlatform();
  // Reads the same shared node App.tsx already fetches once for its own
  // readOnly derivation (#103), instead of running a second independent
  // getVersion() here — the two could otherwise briefly disagree about
  // which version is current.
  const version = useCausl(versionNode);

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
            </div>
          </section>

          {/* B3.7 (#140). This used to be a static row of badges reading
              "7 daily / 13 weekly / 12 monthly / protect known-good": a
              picture of a policy, wired to nothing, and wrong twice over
              once #156 generalized the chain (13 weekly was never a
              default, and the chain is not three fixed tiers). It is now
              the real thing, read from and written to the running
              config. */}
          <RetentionPolicyCard readOnly={readOnly} />

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
                  <Row label="Service version" value={version.data.service} />
                  <Row label="API contract" value={version.data.api} />
                  <Row label="Backup engine" value={version.data.engine} />
                  <Row label="Go toolchain" value={version.data.goVersion} />
                  <Row label="Configuration revision" value={version.data.configRevision} />
                  <Row label="Platform adapter" value={bridge.deployment.adapterVersion} />
                  <Row label="Build commit" value={version.data.buildCommit} />
                </dl>
              ) : version.error ? (
                // versionNode's one fetch is owned by App.tsx, not this page,
                // so there is nothing here to retry (mirrors BackupSetsPage's
                // operations.error inline notice, same reasoning).
                <div className="banner banner--danger" style={{ fontSize: "var(--text-sm)" }}>
                  {"Version information is unavailable (" + version.error.message + ") — details below may be out of date."}
                </div>
              ) : (
                <p style={{ margin: 0, fontSize: 13, color: "var(--text-3)" }}>Loading version information…</p>
              )}
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
 *  here.
 *
 *  #274: a rejected rotation used to read as a wrong current password
 *  whatever the service said, under the literal `cid_rotate_password`. A
 *  rate-limited address and a session that expired while this tab sat open
 *  are both reachable here and neither is a wrong password. */
function describeRotationFailure(e: unknown): OperatorFailure {
  const api = apiErrorOf(e);
  if (api?.code === "UNAUTHENTICATED") {
    // handleRotatePassword answers UNAUTHENTICATED both for a wrong current
    // password and for a session that is no longer valid, and does not say
    // which. Naming both beats naming the wrong one.
    return {
      message: "That current password was not accepted.",
      remediation: "If it is definitely right, this tab's session may have expired: reload the page, sign in again and retry.",
      correlationId: api.correlationId
    };
  }
  return describeFailure(e, "The password was not changed.");
}

function ChangePasswordCard({ readOnly }: { readOnly: boolean }) {
  const api = useApi();
  const [current, setCurrent] = useState("");
  const [next, setNext] = useState("");
  const [confirm, setConfirm] = useState("");
  const [busy, setBusy] = useState(false);
  const [failure, setFailure] = useState<OperatorFailure | null>(null);
  const [success, setSuccess] = useState(false);

  const tooShort = next.length > 0 && next.length < MIN_PASSWORD_LENGTH;
  const mismatch = confirm.length > 0 && confirm !== next;
  const valid = current.length > 0 && next.length >= MIN_PASSWORD_LENGTH && confirm === next;

  const submit = (e: React.FormEvent) => {
    e.preventDefault();
    if (!valid || readOnly) return;
    setBusy(true);
    setFailure(null);
    setSuccess(false);
    api
      .rotatePassword(current, next)
      .then(() => {
        setSuccess(true);
        setCurrent("");
        setNext("");
        setConfirm("");
      })
      .catch((e: unknown) => setFailure(describeRotationFailure(e)))
      .finally(() => setBusy(false));
  };

  return (
    <section className="card">
      <div className="card__header"><h2 className="eyebrow">Administrator password</h2></div>
      <div className="card__body">
        <form onSubmit={submit} style={{ display: "flex", flexDirection: "column", gap: 14 }}>
          <HelpField label="Current password" help={FIELD_HELP.currentPassword}>
            {(helpId) => (
              <input
                className="input"
                type="password"
                aria-describedby={helpId}
                autoComplete="current-password"
                value={current}
                onChange={(e) => setCurrent(e.target.value)}
                disabled={readOnly}
                required
              />
            )}
          </HelpField>
          <HelpField label="New password" help={FIELD_HELP.newPassword}>
            {(helpId) => (
              <>
                <input
                  className="input"
                  type="password"
                  aria-describedby={helpId}
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
              </>
            )}
          </HelpField>
          <HelpField label="Confirm new password" help={FIELD_HELP.confirmNewPassword}>
            {(helpId) => (
              <>
                <input
                  className="input"
                  type="password"
                  aria-describedby={helpId}
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
              </>
            )}
          </HelpField>
          {success ? (
            <div className="banner banner--ok" style={{ fontSize: "var(--text-sm)" }}>
              Password changed. Other signed-in sessions have been signed out.
            </div>
          ) : null}
          {failure ? (
            <ErrorState
              message={failure.message}
              remediation={failure.remediation}
              correlationId={failure.correlationId}
            />
          ) : null}
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
