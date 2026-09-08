/**
 * The two things a panel shows instead of content: nothing to show, and
 * could not be shown.
 *
 * They live together because the mistake they prevent is the same one.
 * A list that is empty because there is nothing yet and a list that is
 * empty because the request failed look identical if both render as blank
 * space, and the operator's next step is completely different. So each
 * gets a shape of its own, and the error one insists on saying what to do
 * as well as what happened.
 *
 * Neither ever shows a stack trace. The most an error offers is the
 * correlation id the failing response actually carried, folded away under
 * advanced details, and it is omitted entirely when the failure carried
 * none, because an id that appears in no log is a false lead.
 */
import { useState } from "react";
import type { ReactNode } from "react";

export function EmptyState({
  title,
  children,
  action
}: {
  title: string;
  children?: ReactNode;
  action?: ReactNode;
}) {
  return (
    <div
      style={{
        padding: "34px 22px", textAlign: "center",
        border: "1px dashed var(--border-strong)", borderRadius: "var(--radius-xl)",
        background: "var(--surface-2)"
      }}
    >
      <div style={{ fontSize: 15, fontWeight: 600 }}>{title}</div>
      {children ? (
        <p
          style={{
            margin: "6px auto 14px", maxWidth: "46ch",
            fontSize: 13, color: "var(--text-2)"
          }}
        >
          {children}
        </p>
      ) : null}
      {action}
    </div>
  );
}

/**
 * The one string this component refuses to treat as an id (#598).
 *
 * `ApiError.correlationId` used to be a required field, so every caller
 * with no id to give wrote one down, and they all wrote the same one. That
 * turned the check below into a check that always passed, and an operator
 * got a disclosure to open with nothing behind it. The constructors are
 * fixed; this stays as the backstop, because the rule belongs where the id
 * is rendered rather than at each of the places that produce one, and the
 * next place that produces one has not been written yet.
 */
const NOT_AN_ID = "unavailable";

/** Copies text through the async clipboard, answering whether it worked.
 *  Every NAS deployment on a plain http:// origin has no
 *  navigator.clipboard at all, and a button whose entire job is getting a
 *  diagnostic off the screen has to say it could not rather than sit there
 *  looking like it worked (#598). */
async function copyToClipboard(text: string): Promise<boolean> {
  try {
    if (!navigator.clipboard?.writeText) return false;
    await navigator.clipboard.writeText(text);
    return true;
  } catch {
    return false;
  }
}

export function ErrorState({
  message,
  remediation,
  correlationId,
  detail,
  onRetry,
  retriedAt
}: {
  message: string;
  remediation?: string;
  /** The id the failing RESPONSE carried, never a literal written here.
   *  Omitted when the failure carried none: a correlation id that matches
   *  nothing in any log is a false lead, and sends whoever quotes it
   *  grepping for a string that was never written (#274). */
  correlationId?: string;
  /** What actually happened, in the technical terms whoever is fixing it
   *  needs: the request, the response, and the exception (#598). Never a
   *  stack trace and never a source path (§37). */
  detail?: string;
  onRetry?(): void;
  /** When the operator last pressed Try again, as they read a clock.
   *  Pressing it against a service that is still down would otherwise
   *  leave the same words on screen with no evidence anything happened,
   *  and a button that looks inert is a button people press harder
   *  (#598). The caller owns this rather than this component, because this
   *  component unmounts for the moment the retry is in flight. */
  retriedAt?: string;
}) {
  const [copied, setCopied] = useState<"idle" | "done" | "failed">("idle");

  const id = correlationId === NOT_AN_ID ? undefined : correlationId;
  const lines = [id ? "correlation id " + id : undefined, detail]
    .filter((l): l is string => !!l)
    .join("\n");
  // Nothing to disclose is not a disclosure with nothing in it. A failure
  // that carried neither an id nor a detail offers no panel at all.
  const advanced = lines === "" ? undefined : lines;

  return (
    <div className="banner banner--danger" role="alert" style={{ flexDirection: "column" }}>
      <div style={{ display: "flex", gap: 12 }}>
        <span aria-hidden="true" style={{ color: "var(--danger)" }}>{"\u2715"}</span>
        <div>
          <div style={{ fontWeight: 600, fontSize: 13.5 }}>{message}</div>
          {remediation ? (
            <div style={{ marginTop: 4, fontSize: 13, color: "var(--text-2)" }}>{remediation}</div>
          ) : null}
          {/* Advanced details only — never a stack trace (§37). */}
          {advanced ? (
            <details style={{ marginTop: 8, fontSize: "var(--text-sm)", color: "var(--text-3)" }}>
              <summary style={{ cursor: "pointer" }}>Advanced details</summary>
              <div className="mono" style={{ marginTop: 6, whiteSpace: "pre-wrap" }}>{advanced}</div>
              {/* The next thing an operator does with a failure is paste
                  it somewhere, and retyping four lines of exception text
                  off a screen is how the useful half gets left out. */}
              <button
                className="btn btn--sm"
                style={{ marginTop: 8 }}
                onClick={() => {
                  void copyToClipboard(message + "\n" + (remediation ? remediation + "\n" : "") + advanced).then(
                    (ok) => setCopied(ok ? "done" : "failed")
                  );
                }}
              >
                Copy details
              </button>
              {copied === "done" ? (
                <span role="status" style={{ marginLeft: 8 }}>Copied</span>
              ) : null}
              {copied === "failed" ? (
                <span role="status" style={{ marginLeft: 8 }}>
                  Could not copy. Select the text above and copy it by hand.
                </span>
              ) : null}
            </details>
          ) : null}
          {onRetry ? (
            <div style={{ marginTop: 10, display: "flex", alignItems: "center", gap: 8 }}>
              <button className="btn btn--sm" onClick={onRetry}>
                Try again
              </button>
              {retriedAt ? (
                <span role="status" style={{ fontSize: "var(--text-sm)", color: "var(--text-3)" }}>
                  {"Tried again at " + retriedAt + ", and it failed the same way."}
                </span>
              ) : null}
            </div>
          ) : null}
        </div>
      </div>
    </div>
  );
}
