import { useState } from "react";
import { Link } from "react-router-dom";
import { useApi } from "@shared/api/ApiContext";
import { bootstrapTokenFromLocation } from "@shared/api/client";
import { apiErrorOf, describeFailure } from "@shared/api/failure";
import type { OperatorFailure } from "@shared/api/failure";
import { AuthFrame } from "./LoginPage";
import { ErrorState } from "@shared/components/EmptyState";
import { HelpField } from "@shared/components/FieldHelp";
import { FIELD_HELP } from "@shared/components/fieldHelpCopy";

const MIN_LENGTH = 12;

/**
 * Issue #274. Enrolment refuses for several different reasons and only one
 * of them is "these credentials are no good", which is the only one this
 * page used to report. An operator who opened the link the engine printed,
 * after the token had lapsed, was told the administrator account could not
 * be created and left retyping a password that was never the problem.
 *
 * The three states the service folds into BOOTSTRAP_TOKEN_INVALID are
 * missing, expired and already-used, and they are separated here as far as
 * the wire allows:
 *
 *   - MISSING is settled in the browser. The token travels in the link's
 *     own query string (client.ts), so a page opened without one is a
 *     thing this page can see for itself, and it gets its own message.
 *   - ALREADY USED, in the sense that matters to an operator (somebody has
 *     enrolled, so stop trying and sign in), is a separate code already:
 *     handleEnroll checks for an existing administrator BEFORE the token
 *     and answers ENROLLMENT_CLOSED.
 *   - EXPIRED and a spent-but-unenrolled token are not distinguished on
 *     the wire, and deliberately are not made to be. bootstrapIssuer.consume
 *     returns one boolean, both mean the printed link is dead, and both are
 *     recovered the same way: restart, read the new link out of the log.
 *     A code that changes no advice is not worth an oracle on a pre-auth
 *     route. So one message names both, honestly, rather than picking one
 *     and being wrong half the time.
 */
export interface EnrollmentFailure extends OperatorFailure {
  /** Set on the one refusal that has somewhere to send the operator. */
  offerSignIn?: boolean;
}

export function describeEnrollmentFailure(e: unknown, linkCarriedToken: boolean): EnrollmentFailure {
  const api = apiErrorOf(e);
  switch (api?.code) {
    case "BOOTSTRAP_TOKEN_INVALID":
      return linkCarriedToken
        ? {
            message: "This enrolment link has expired or has already been used.",
            remediation:
              "The username and password were not the problem: Backup Manager checks the one-time token in the link before it looks at them, and nothing was created. Restart Backup Manager and open the fresh enrolment link it prints to its own log, then enter these details again.",
            correlationId: api.correlationId
          }
        : {
            message: "This page was opened without an enrolment token.",
            remediation:
              "Backup Manager prints a one-time enrolment link to its own log when it starts, ending in ?token=… . Open that link rather than this page. If the log has already scrolled past it, restart Backup Manager and it prints a fresh one.",
            correlationId: api.correlationId
          };
    case "ENROLLMENT_CLOSED":
      return {
        message: "An administrator account already exists on this instance.",
        remediation:
          "Enrolment happens once and cannot be repeated. Sign in with that account instead; nothing here can create a second one.",
        correlationId: api.correlationId,
        offerSignIn: true
      };
    default:
      return describeFailure(e, "The administrator account could not be created.");
  }
}

export function EnrollmentPage({ onEnrolled }: { onEnrolled(): void }) {
  const api = useApi();
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [confirm, setConfirm] = useState("");
  const [busy, setBusy] = useState(false);
  const [failure, setFailure] = useState<EnrollmentFailure | null>(null);

  const tooShort = password.length > 0 && password.length < MIN_LENGTH;
  const mismatch = confirm.length > 0 && confirm !== password;
  const valid = username.length > 0 && password.length >= MIN_LENGTH && confirm === password;

  const submit = (e: React.FormEvent) => {
    e.preventDefault();
    if (!valid) return;
    setBusy(true);
    setFailure(null);
    api
      .enrollAdministrator(username, password)
      .then(onEnrolled)
      .catch((e: unknown) => setFailure(describeEnrollmentFailure(e, bootstrapTokenFromLocation() !== null)))
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
        <HelpField label="Username" help={FIELD_HELP.enrollUsername}>
          {(helpId) => (
            <input className="input input--mono" aria-describedby={helpId} autoComplete="username" value={username} onChange={(e) => setUsername(e.target.value)} required />
          )}
        </HelpField>
        <HelpField label="Password" help={FIELD_HELP.enrollPassword}>
          {(helpId) => (
            <>
              <input className="input" type="password" aria-describedby={helpId} autoComplete="new-password" value={password} onChange={(e) => setPassword(e.target.value)} required />
              {tooShort ? (
                <span style={{ fontSize: "var(--text-sm)", color: "var(--danger)" }}>
                  {"Minimum " + MIN_LENGTH + " characters."}
                </span>
              ) : null}
            </>
          )}
        </HelpField>
        <HelpField label="Confirm password" help={FIELD_HELP.enrollConfirm}>
          {(helpId) => (
            <>
              <input className="input" type="password" aria-describedby={helpId} autoComplete="new-password" value={confirm} onChange={(e) => setConfirm(e.target.value)} required />
              {mismatch ? (
                <span style={{ fontSize: "var(--text-sm)", color: "var(--danger)" }}>
                  Passwords do not match.
                </span>
              ) : null}
            </>
          )}
        </HelpField>
        <div className="banner banner--info" style={{ fontSize: "var(--text-sm)", color: "var(--text-2)" }}>
          <span aria-hidden="true" style={{ color: "var(--text-3)" }}>i</span>
          <span>
            {"Minimum " + MIN_LENGTH + " characters. Credentials are stored on this NAS; nothing is sent off the device."}
          </span>
        </div>
        {failure ? (
          <div style={{ display: "flex", flexDirection: "column", gap: 8 }}>
            <ErrorState
              message={failure.message}
              remediation={failure.remediation}
              correlationId={failure.correlationId}
            />
            {/* The one refusal with somewhere to send the operator. */}
            {failure.offerSignIn ? (
              <Link to="/" style={{ fontSize: "var(--text-sm)" }}>
                Go to sign in
              </Link>
            ) : null}
          </div>
        ) : null}
        <button className="btn btn--primary" type="submit" disabled={!valid || busy} style={{ height: 40 }}>
          {busy ? "Creating…" : "Create administrator"}
        </button>
      </form>
    </AuthFrame>
  );
}
