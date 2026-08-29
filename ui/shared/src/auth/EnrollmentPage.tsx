import { useState } from "react";
import { useApi } from "@shared/api/ApiContext";
import { AuthFrame } from "./LoginPage";
import { ErrorState } from "@shared/components/EmptyState";

const MIN_LENGTH = 12;

export function EnrollmentPage({ onEnrolled }: { onEnrolled(): void }) {
  const api = useApi();
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [confirm, setConfirm] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const tooShort = password.length > 0 && password.length < MIN_LENGTH;
  const mismatch = confirm.length > 0 && confirm !== password;
  const valid = username.length > 0 && password.length >= MIN_LENGTH && confirm === password;

  const submit = (e: React.FormEvent) => {
    e.preventDefault();
    if (!valid) return;
    setBusy(true);
    api
      .enrollAdministrator(username, password)
      .then(onEnrolled)
      .catch(() => setError("The administrator account could not be created."))
      .finally(() => setBusy(false));
  };

  return (
    <AuthFrame>
      <div className="eyebrow" style={{ fontSize: "var(--text-xs)" }}>First run</div>
      <h1 style={{ margin: "8px 0 6px", fontSize: 21 }}>Create Backup Manager administrator</h1>
      <p style={{ margin: "0 0 22px", color: "var(--text-2)", fontSize: 13 }}>
        This account manages Backup Manager only. It is separate from your NAS
        operating-system account.
      </p>
      <form onSubmit={submit} style={{ display: "flex", flexDirection: "column", gap: 14 }}>
        <label className="field">
          <span className="field__label">Username</span>
          <input className="input input--mono" autoComplete="username" value={username} onChange={(e) => setUsername(e.target.value)} required />
        </label>
        <label className="field">
          <span className="field__label">Password</span>
          <input className="input" type="password" autoComplete="new-password" value={password} onChange={(e) => setPassword(e.target.value)} required />
          {tooShort ? (
            <span style={{ fontSize: "var(--text-sm)", color: "var(--danger)" }}>
              {"Minimum " + MIN_LENGTH + " characters."}
            </span>
          ) : null}
        </label>
        <label className="field">
          <span className="field__label">Confirm password</span>
          <input className="input" type="password" autoComplete="new-password" value={confirm} onChange={(e) => setConfirm(e.target.value)} required />
          {mismatch ? (
            <span style={{ fontSize: "var(--text-sm)", color: "var(--danger)" }}>
              Passwords do not match.
            </span>
          ) : null}
        </label>
        <div className="banner banner--info" style={{ fontSize: "var(--text-sm)", color: "var(--text-2)" }}>
          <span aria-hidden="true" style={{ color: "var(--text-3)" }}>i</span>
          <span>
            {"Minimum " + MIN_LENGTH + " characters. Credentials are stored on this NAS; nothing is sent off the device."}
          </span>
        </div>
        {error ? <ErrorState message={error} correlationId="cid_enroll" /> : null}
        <button className="btn btn--primary" type="submit" disabled={!valid || busy} style={{ height: 40 }}>
          {busy ? "Creating…" : "Create administrator"}
        </button>
      </form>
    </AuthFrame>
  );
}
