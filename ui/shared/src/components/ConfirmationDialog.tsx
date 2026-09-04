import { useEffect, useRef, useState } from "react";
import type { ReactNode } from "react";

/**
 * Destructive confirmations must name the consequence in the button (§35).
 * "OK" is never acceptable, so confirmLabel is required.
 *
 * # `confirmPhrase`: type this exact string or the button stays off
 *
 * Opt-in, and off for every caller that does not ask for it, because most
 * confirmations here are "are you sure" about something recoverable and
 * making an operator type a name to pause a backup set would be theatre.
 *
 * It exists for the confirmations that are reached from a LIST. A dialog
 * opened from one row of many has to prove WHICH row it is about to act
 * on, and naming the row in the copy only works if the operator reads the
 * copy. Retyping the row's own identity cannot be done by accident, and
 * it cannot be done for the wrong row: the phrase is that row's, so the
 * near-miss of confirming the set below the one you meant is exactly the
 * case it refuses.
 *
 * The comparison is `===` on the raw value. Not trimmed, not
 * case-folded: a confirmation that accepts a near-miss is not confirming
 * anything, and the phrase is on screen a line above the box, so there is
 * nothing to be kind about.
 *
 * `disabled` still applies on top of it. A dialog mid-submit is disabled
 * whatever is typed, which is what stops a second click from sending a
 * second destructive request.
 */
export function ConfirmationDialog({
  open,
  eyebrow,
  title,
  confirmLabel,
  cancelLabel = "Cancel",
  destructive = false,
  disabled = false,
  confirmPhrase,
  confirmPhraseLabel = "Type the name to confirm",
  onConfirm,
  onCancel,
  children
}: {
  open: boolean;
  eyebrow?: string;
  title: string;
  confirmLabel: string;
  cancelLabel?: string;
  destructive?: boolean;
  disabled?: boolean;
  /** When set, the confirm button stays disabled until the operator types
   *  exactly this. Omit it and the dialog behaves as it always has. */
  confirmPhrase?: string;
  /** The label on that box. Says what to type, so the requirement is not
   *  a puzzle. A node rather than a string so a caller can set the phrase
   *  itself in mono inside the label, which keeps the phrase in exactly
   *  one place: a second, prettier copy of it in the body would be a
   *  second copy that can disagree with the one being compared. */
  confirmPhraseLabel?: ReactNode;
  onConfirm(): void;
  onCancel(): void;
  children: ReactNode;
}) {
  const cancelRef = useRef<HTMLButtonElement>(null);
  const [typed, setTyped] = useState("");

  // Cleared on every open AND on every close, and whenever the phrase
  // itself changes. Without this the box keeps what was typed the last
  // time the dialog was open, so a second removal opened from a different
  // row would arrive pre-confirmed with the PREVIOUS row's phrase still
  // in it, and one click would remove a set nobody typed the name of.
  // Clearing on close is the half that closes the window: the reset then
  // happens while the dialog is not on screen, rather than one passive
  // effect after a reopened dialog has already painted an enabled button.
  useEffect(() => {
    setTyped("");
  }, [open, confirmPhrase]);

  const phraseSatisfied = confirmPhrase === undefined || typed === confirmPhrase;

  // Focus lands on the SAFE action, never the destructive one.
  useEffect(() => {
    if (open) cancelRef.current?.focus();
  }, [open]);

  useEffect(() => {
    if (!open) return;
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") onCancel();
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [open, onCancel]);

  if (!open) return null;

  return (
    <div className="dialog-scrim" onClick={(e) => e.target === e.currentTarget && onCancel()}>
      <div
        role="dialog"
        aria-modal="true"
        aria-labelledby="confirm-title"
        className={"dialog" + (destructive ? " dialog--destructive" : "")}
        style={{ maxWidth: 476 }}
      >
        <div style={{ padding: "20px 22px 0" }}>
          {eyebrow ? (
            <div
              className="eyebrow"
              style={{
                color: destructive ? "var(--danger)" : "var(--text-3)",
                fontSize: "var(--text-xs)", letterSpacing: "0.09em"
              }}
            >
              {eyebrow}
            </div>
          ) : null}
          <h2 id="confirm-title" style={{ margin: "8px 0 0", fontSize: 18 }}>{title}</h2>
        </div>
        <div
          style={{
            padding: "14px 22px 20px", display: "flex", flexDirection: "column",
            gap: 12, fontSize: 13.5
          }}
        >
          {children}
          {confirmPhrase === undefined ? null : (
            <label className="field">
              <span className="field__label">{confirmPhraseLabel}</span>
              <input
                className="input input--mono"
                type="text"
                autoComplete="off"
                autoCorrect="off"
                autoCapitalize="none"
                spellCheck={false}
                value={typed}
                onChange={(e) => setTyped(e.target.value)}
              />
            </label>
          )}
        </div>
        <div
          className="card__footer"
          style={{ display: "flex", justifyContent: "flex-end", gap: 9, borderRadius: "0 0 10px 10px" }}
        >
          <button ref={cancelRef} className="btn" onClick={onCancel}>{cancelLabel}</button>
          <button
            className={"btn " + (destructive ? "btn--destructive-confirm" : "btn--primary")}
            onClick={onConfirm}
            disabled={disabled || !phraseSatisfied}
          >
            {confirmLabel}
          </button>
        </div>
      </div>
    </div>
  );
}
