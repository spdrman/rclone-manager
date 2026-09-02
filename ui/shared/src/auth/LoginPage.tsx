import { useState } from "react";
import { Link } from "react-router-dom";
import { useApi } from "@shared/api/ApiContext";
import { apiErrorOf, describeFailure } from "@shared/api/failure";
import type { OperatorFailure } from "@shared/api/failure";
import { Logo, Wordmark } from "@shared/components/Logo";
import { ErrorState } from "@shared/components/EmptyState";
import { HelpField } from "@shared/components/FieldHelp";
import { PasswordInput } from "@shared/components/PasswordInput";
import { FIELD_HELP } from "@shared/components/fieldHelpCopy";

/** Issue #274, one view over from the enrolment page it was filed against:
 *  a wrong password and a rate-limited address are different problems with
 *  different answers, and both used to read as a wrong password under a
 *  correlation id, the literal `cid_login`, that no log has ever held. */
export function describeLoginFailure(e: unknown): OperatorFailure {
  const api = apiErrorOf(e);
  if (api?.code === "UNAUTHENTICATED") {
    // The service's own words for this are already exact, and this is the
    // one refusal where saying less is the point: which half was wrong is
    // deliberately not distinguished, by the handler or here.
    return {
      message: "That username and password combination was not accepted.",
      correlationId: api.correlationId
    };
  }
  return describeFailure(e, "Signing in did not succeed.");
}

/** §30 — deliberately NOT styled like a NAS system login. An operator must never
 *  believe they are handing NAS OS credentials to this app. */
export function LoginPage({ onSignedIn }: { onSignedIn(): void }) {
  const api = useApi();
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [failure, setFailure] = useState<OperatorFailure | null>(null);
  const [busy, setBusy] = useState(false);

  const submit = (e: React.FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setFailure(null);
    api
      .login(username, password)
      .then(onSignedIn)
      .catch((e: unknown) => setFailure(describeLoginFailure(e)))
      .finally(() => setBusy(false));
  };

  return (
    <AuthFrame>
      <h1 style={{ margin: "0 0 6px", fontSize: 21 }}>Sign in</h1>
      <p style={{ margin: "0 0 22px", color: "var(--text-2)", fontSize: 13 }}>
        Backup Manager local account — <strong style={{ color: "var(--text)" }}>not</strong> your
        NAS operating-system login.
      </p>
      <form onSubmit={submit} style={{ display: "flex", flexDirection: "column", gap: 14 }}>
        <HelpField label="Username" help={FIELD_HELP.loginUsername}>
          {(helpId) => (
            <input
              className="input input--mono"
              aria-describedby={helpId}
              autoComplete="username"
              value={username}
              onChange={(e) => setUsername(e.target.value)}
              required
            />
          )}
        </HelpField>
        <HelpField label="Password" help={FIELD_HELP.loginPassword}>
          {(helpId, field) => (
            <PasswordInput
              label={field.label}
              labelledBy={field.id}
              autoComplete="current-password"
              describedBy={helpId}
              value={password}
              onChange={setPassword}
              required
            />
          )}
        </HelpField>
        {failure ? (
          <ErrorState
            message={failure.message}
            remediation={failure.remediation}
            correlationId={failure.correlationId}
          />
        ) : null}
        <button className="btn btn--primary" type="submit" disabled={busy} style={{ height: 40 }}>
          {busy ? "Signing in…" : "Sign in"}
        </button>
      </form>
      <p style={{ margin: "18px 0 0", fontSize: "var(--text-sm)", color: "var(--text-3)" }}>
        First time here? <Link to="/enroll">Create the administrator account</Link>.
      </p>
    </AuthFrame>
  );
}

export function AuthFrame({ children }: { children: React.ReactNode }) {
  return (
    <div style={{ minHeight: "100vh", display: "grid", placeItems: "center", padding: "48px 24px", background: "var(--bg)" }}>
      <div style={{ width: "100%", maxWidth: 452, display: "flex", flexDirection: "column", gap: 20 }}>
        <div style={{ display: "flex", alignItems: "center", gap: 11 }}>
          <Logo size={27} title="Backup Manager" />
          <Wordmark size={15} />
        </div>
        <div className="card" style={{ padding: 28 }}>{children}</div>
      </div>
    </div>
  );
}
